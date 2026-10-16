package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/logger"
)

// TestCrashOutputPath pins where the crash file goes: beside the log, named
// after it.
//
// Mutant: name it beside the binary or under a fixed name — the second row.
func TestCrashOutputPath(t *testing.T) {
	dir := filepath.Join("data", "logs")
	for _, tc := range []struct{ log, want string }{
		{filepath.Join(dir, "moombox.log"), filepath.Join(dir, "moombox-crash.log")},
		{filepath.Join(dir, "archiver"), filepath.Join(dir, "archiver-crash.log")},
	} {
		if got := crashOutputPath(tc.log); got != tc.want {
			t.Errorf("crashOutputPath(%q) = %q, want %q", tc.log, got, tc.want)
		}
	}
}

// crashHelperEnv makes the re-executed test binary arm the crash output at the
// path it names and then crash the way crashKindEnv says — the kinds of crash
// that never reach the logger.
const (
	crashHelperEnv = "MOOMBOX_CRASH_HELPER"
	crashKindEnv   = "MOOMBOX_CRASH_KIND"
)

// crashSecret is in the helper's panic message, inside a URL's userinfo and
// query, and as a bare PO token: the excerpt must carry none of it.
const crashSecret = "hunter2-SECRET"

// crashHelper is the helper process's whole life; it never returns.
func crashHelper(path string) {
	if err := armCrashOutput(path); err != nil {
		os.Exit(97)
	}
	go func() {
		if os.Getenv(crashKindEnv) == "fatal" {
			var mu sync.Mutex
			mu.Unlock() // fatal error: sync: unlock of unlocked mutex — no recover can catch it
		}
		panic("probe crash: POST https://user:" + crashSecret + "@hooks.example.com/x?token=" + crashSecret +
			" failed, pot=" + crashSecret)
	}()
	time.Sleep(time.Minute)
	os.Exit(96)
}

// TestARealCrashReachesTheNextStartsLog runs a process that arms the crash
// output and then dies — of a panic in a goroutine with no recover, and of a
// fatal runtime error — as such a crash ends the child in production, and
// reads what it left the way the next start does. The runtime writes such a
// crash to stderr only — in TUI mode the alternate screen the respawned TUI
// clears — so moombox.log had nothing but the exit code, and the Recovered
// From Crash notice sent the operator to logs that did not hold the cause
// (W28-A04). A fatal error's own "fatal error: …" line never reaches the
// crash file (the runtime prints it before it counts itself as crashing), so
// its cause is named by the frame it struck in.
//
// Mutants: armCrashOutput not calling debug.SetCrashOutput (the file stays
// empty); redactCrashText returning its input (the secret reaches the
// excerpt); takePreviousCrash not moving the output aside (no .1, and the
// next crash's capture would truncate it); crashCause taking the first line
// whatever it is (the fatal row's cause is a bare "goroutine 9 [running]:").
func TestARealCrashReachesTheNextStartsLog(t *testing.T) {
	if path := os.Getenv(crashHelperEnv); path != "" {
		crashHelper(path)
	}

	for _, tc := range []struct {
		kind, wantCause string
	}{
		{"panic", "panic: probe crash"},
		{"fatal", "fatal error in internal/sync.(*Mutex).unlockSlow"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			crashPath := filepath.Join(t.TempDir(), "moombox-crash.log")
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestARealCrashReachesTheNextStartsLog$")
			cmd.Env = append(os.Environ(), crashHelperEnv+"="+crashPath, crashKindEnv+"="+tc.kind)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
				t.Fatalf("the helper ended with %v, want the runtime's exit 2 for a crash; output:\n%s", err, out)
			}

			prev, err := takePreviousCrash(crashPath)
			if err != nil {
				t.Fatalf("takePreviousCrash: %v", err)
			}
			if !prev.captured() {
				t.Fatalf("the crash left nothing in %s: its output went to stderr only", crashPath)
			}
			if !strings.HasPrefix(prev.cause, tc.wantCause) {
				t.Errorf("cause = %q, want it to start %q", prev.cause, tc.wantCause)
			}
			if !strings.Contains(prev.excerpt, "goroutine ") {
				t.Errorf("excerpt has no goroutine trace:\n%s", prev.excerpt)
			}
			if strings.Contains(prev.excerpt, crashSecret) || strings.Contains(prev.cause, crashSecret) {
				t.Errorf("the excerpt carries the credential the panic quoted:\n%s", prev.excerpt)
			}
			if want := keptCrashPath(crashPath); prev.kept != want {
				t.Errorf("kept = %q, want %q", prev.kept, want)
			}
			if _, err := os.Stat(keptCrashPath(crashPath)); err != nil {
				t.Errorf("the crash output was not kept: %v", err)
			}
			if _, err := os.Stat(crashPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the crash file is still in place (%v): the next start's capture would truncate it", err)
			}
		})
	}
}

// TestCrashCause pins how a crash is named from its output: the runtime's
// first line, or — for a fatal error, whose own line reached stderr only — the
// first frame of the dying goroutine below the runtime's throw and fatal.
//
// Mutants: take the first line whatever it is (the trace rows); stop skipping
// the throw/fatal frames (the cause names runtime.throw).
func TestCrashCause(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"a panic", "panic: boom\n\ngoroutine 1 [running]:\nmain.main()\n", "panic: boom"},
		{"a fatal signal", "SIGABRT: abort\nPC=0x49b9a1 m=0 sigcode=0\n", "SIGABRT: abort"},
		{"a fatal error, message lost",
			"goroutine 7 [running]:\ninternal/runtime/maps.fatal({0x4a1f5b?, 0x0?})\n\t/go/src/runtime/panic.go:1053 +0x18\n" +
				"runtime.throw({0x1?, 0x2?})\n\t/go/src/runtime/panic.go:1100 +0x18\n" +
				"runtime.mapassign_faststr(0xc0000a4000, {0x4b1c2a, 0x3})\n\t/go/src/internal/runtime/maps/x.go:12 +0x9\n",
			"fatal error in runtime.mapassign_faststr"},
	} {
		got := crashCause(strings.Split(tc.out, "\n"))
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: crashCause = %q, want it to start %q", tc.name, got, tc.want)
		}
	}
}

// TestTakePreviousCrashBoundsWhatItKeeps pins the bounds: moombox.log gets the
// beginning of the output — the cause and the goroutine that died, which the
// runtime writes first — cut at a line and saying how much it left out; the
// kept copy is cut to crashKeepBytes and replaces the crash before it; a start
// after a run that did not crash finds nothing.
//
// Mutants: read the whole file into the excerpt (the excerpt bound); drop
// the line-boundary cut (the excerpt ends mid-line); drop the os.Truncate
// (the kept-size bound); report an empty file as a crash.
func TestTakePreviousCrashBoundsWhatItKeeps(t *testing.T) {
	dir := t.TempDir()
	crashPath := filepath.Join(dir, "moombox-crash.log")

	if prev, err := takePreviousCrash(crashPath); err != nil || prev.captured() {
		t.Fatalf("no crash file: %+v, %v; want nothing", prev, err)
	}
	if err := os.WriteFile(crashPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if prev, err := takePreviousCrash(crashPath); err != nil || prev.captured() {
		t.Fatalf("an empty crash file: %+v, %v; want nothing", prev, err)
	}

	if err := os.WriteFile(keptCrashPath(crashPath), []byte("an older crash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("fatal error: concurrent map writes\n\ngoroutine 42 [running]:\n")
	for b.Len() < crashKeepBytes+crashKeepBytes/2 {
		b.WriteString("github.com/vampiricwulf/Moombox/internal/worker.(*DownloadWorker).run(0xc000123456)\n\t/src/internal/worker/worker.go:123 +0x45\n")
	}
	if err := os.WriteFile(crashPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	prev, err := takePreviousCrash(crashPath)
	if err != nil {
		t.Fatalf("takePreviousCrash: %v", err)
	}
	if prev.cause != "fatal error: concurrent map writes" {
		t.Errorf("cause = %q", prev.cause)
	}
	if len(prev.excerpt) > crashExcerptBytes+64 {
		t.Errorf("excerpt is %d bytes, want about %d", len(prev.excerpt), crashExcerptBytes)
	}
	body, tail, ok := strings.Cut(prev.excerpt, "… ")
	if !ok || !strings.HasSuffix(tail, "more bytes") {
		t.Errorf("a cut excerpt does not say how much it left out; it ends %q", prev.excerpt[max(0, len(prev.excerpt)-80):])
	}
	if !strings.HasSuffix(body, "+0x45\n") {
		t.Errorf("the excerpt is not cut at a line boundary; it ends %q", body[max(0, len(body)-40):])
	}
	info, err := os.Stat(prev.kept)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != crashKeepBytes {
		t.Errorf("kept output is %d bytes, want it cut to %d", info.Size(), crashKeepBytes)
	}
	kept, _ := os.ReadFile(prev.kept)
	if !strings.HasPrefix(string(kept), "fatal error: concurrent map writes") {
		t.Errorf("the kept output is not this crash's (the older crash was not replaced, or its beginning was cut)")
	}
}

// TestRedactCrashText pins the redaction crash output gets on its way to the
// log and a notification: every URL cut to its origin — a panic's text can
// quote a webhook URL, a userinfo password or a signed media URL — and a GVS
// PO token outside any URL cut too.
//
// Mutants: drop the URL rewrite (the URL rows); drop the PoTokenText (the
// bare-token row).
func TestRedactCrashText(t *testing.T) {
	for _, in := range []string{
		"panic: POST https://user:" + crashSecret + "@hooks.example.com/api/webhooks/1/" + crashSecret + " failed",
		"panic: Get \"https://rr1.googlevideo.com/videoplayback?ip=1.2.3.4&sig=" + crashSecret + "\": EOF",
		"panic: tgram://" + crashSecret + "/chat",
		"panic: token pot=" + crashSecret + " refused",
	} {
		got := redactCrashText(in)
		if strings.Contains(got, crashSecret) || strings.Contains(got, "1.2.3.4") {
			t.Errorf("redactCrashText(%q) = %q: the credential survived", in, got)
		}
		if !strings.HasPrefix(got, "panic: ") {
			t.Errorf("redactCrashText(%q) = %q: it cut more than the credential", in, got)
		}
	}
	if got := redactCrashText("fatal error: concurrent map writes"); got != "fatal error: concurrent map writes" {
		t.Errorf("text with no URL or token came back as %q", got)
	}
}

// TestCrashRecoveredNotice pins the Recovered From Crash text. It used to say
// "check the logs for the cause" whatever the logs held. Now it names the
// cause and where the output is when a crash was captured, and says plainly
// when the process left none — a SIGKILL or the OOM killer report nothing —
// rather than sending the operator to look.
//
// Mutants: the old fixed text (both rows); the captured check inverted.
func TestCrashRecoveredNotice(t *testing.T) {
	kept := filepath.Join("data", "moombox-crash.log.1")
	desc, fields := crashRecoveredNotice("2", previousCrash{cause: "panic: boom", excerpt: "panic: boom", size: 40, kept: kept}, "moombox.log")
	if !strings.Contains(desc, "code 2") || !strings.Contains(desc, "moombox.log") || !strings.Contains(desc, kept) {
		t.Errorf("captured: desc = %q, want the code, the log and the kept file", desc)
	}
	if len(fields) != 1 || fields[0].Name != "Cause" || fields[0].Value != "panic: boom" {
		t.Errorf("captured: fields = %+v, want the cause", fields)
	}

	desc, fields = crashRecoveredNotice("-1", previousCrash{}, "moombox.log")
	if !strings.Contains(desc, "code -1") || !strings.Contains(desc, "no crash output") {
		t.Errorf("nothing captured: desc = %q, want it to say the process left no crash output", desc)
	}
	if strings.Contains(desc, "check the logs") || fields != nil {
		t.Errorf("nothing captured: desc = %q, fields = %+v; want no pointer at logs that hold nothing", desc, fields)
	}
}

// TestCaptureCrashOutputLogsThePreviousCrashAndArms pins the boot step: what a
// crash left is logged (cause and output) into moombox.log, and a fresh, empty
// crash file is armed for this process.
//
// Mutants: drop the Error line; drop the armCrashOutput call.
func TestCaptureCrashOutputLogsThePreviousCrashAndArms(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "moombox.log")
	crashPath := crashOutputPath(logPath)
	if err := os.WriteFile(crashPath, []byte("panic: probe\n\ngoroutine 9 [running]:\nmain.f()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logPath, "INFO", 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Disarm before the temp dir goes: the runtime holds a descriptor of the
	// file, which Windows will not delete while it is open.
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })

	s := &runState{log: log}
	prev := s.captureCrashOutput(logPath)
	log.Close()
	if !prev.captured() || prev.cause != "panic: probe" {
		t.Fatalf("captureCrashOutput = %+v, want the probe crash", prev)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "the previous run crashed") || !strings.Contains(string(logged), "goroutine 9") {
		t.Errorf("moombox.log has no account of the previous crash:\n%s", logged)
	}
	info, err := os.Stat(crashPath)
	if err != nil || info.Size() != 0 {
		t.Errorf("no fresh crash file armed for this process (%v)", err)
	}
}

// TestCrashOutputIsWired pins the two call sites: initServices captures the
// crash output as soon as the logger is open, and run()'s crash-respawn notice
// is crashRecoveredNotice's.
//
// Mutants: the captureCrashOutput call deleted; the old fixed notice text back
// in run().
func TestCrashOutputIsWired(t *testing.T) {
	callsIn := func(file, fn string) []string {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		var out []string
		for _, decl := range f.Decls {
			if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == fn && d.Body != nil {
				ast.Inspect(d.Body, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok {
						out = append(out, callText(call.Fun))
					}
					return true
				})
			}
		}
		return out
	}
	contains := func(list []string, want string) bool {
		for _, c := range list {
			if c == want {
				return true
			}
		}
		return false
	}
	if !contains(callsIn("services.go", "initServices"), "s.captureCrashOutput") {
		t.Error("initServices does not capture the crash output")
	}
	if !contains(callsIn("main.go", "run"), "crashRecoveredNotice") {
		t.Error("run() does not build the Recovered From Crash notice with crashRecoveredNotice")
	}
}
