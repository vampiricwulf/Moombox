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
		switch os.Getenv(crashKindEnv) {
		case "fatal":
			var mu sync.Mutex
			mu.Unlock() // fatal error: sync: unlock of unlocked mutex — no recover can catch it
		case "stack overflow":
			debug.SetMaxStack(1 << 20)
			crashRecurse(0) // fatal error: stack overflow, raised on the system stack
		case "out of memory":
			crashHeld = crashAllocHuge() // fatal error: out of memory, thrown on the goroutine's own stack
		}
		panic("probe crash: POST https://user:" + crashSecret + "@hooks.example.com/x?token=" + crashSecret +
			" failed, pot=" + crashSecret)
	}()
	time.Sleep(time.Minute)
	os.Exit(96)
}

// crashRecurse recurses until the goroutine's stack is past its limit (n
// counts up from 0, so the stop is never reached).
//
//go:noinline
func crashRecurse(n int) int {
	if n < 0 {
		return 0
	}
	var pad [256]byte
	pad[n%len(pad)] = byte(n)
	return crashRecurse(n+1) + int(pad[0])
}

// crashHeld keeps crashAllocHuge's result reachable.
var crashHeld [][]byte

// crashAllocHuge asks for one block just under the runtime's largest
// allocation (256 TiB less 64 MiB), more than the user address space of any
// platform Moombox ships for: reserving it fails before the runtime creates
// any arena metadata, so it throws out of memory having committed nothing.
// It must stay a block no machine can reserve. One that can be reserved (32
// TiB, as this once asked for) is not free: the runtime commits about 68 KiB
// of arena metadata per 64 MiB of it before it fails, ~34 GiB for 32 TiB,
// which Windows charges against the whole machine's commit limit — the
// processes go test runs beside this one then failed with "The paging file
// is too small".
//
//go:noinline
func crashAllocHuge() [][]byte {
	return [][]byte{make([]byte, 1<<48-1<<26)}
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
// its cause is named by the frame it struck in: the dying goroutine's first
// frame outside the runtime — for a stack overflow, whose output starts at
// the system stack's "runtime stack:", the goroutine listed after that block
// (once named "runtime stack:" itself); for running out of memory, the
// goroutine that asked for the block.
//
// Mutants: armCrashOutput not calling debug.SetCrashOutput (the file stays
// empty); redactCrashText returning its input (the secret reaches the
// excerpt); takePreviousCrash not moving the output aside (no .1, and the
// next crash's capture would truncate it); crashCause taking the first line
// whatever it is (the fatal and out-of-memory rows' cause is a bare
// "goroutine 9 [running]:", the stack-overflow row's "runtime stack:");
// crashCause not passing over the runtime's own frames (the out-of-memory
// row names runtime.(*mcache).allocLarge).
func TestARealCrashReachesTheNextStartsLog(t *testing.T) {
	if path := os.Getenv(crashHelperEnv); path != "" {
		crashHelper(path)
	}

	for _, tc := range []struct {
		kind, wantCause, wantIn string
	}{
		{"panic", "panic: probe crash", ""},
		{"fatal", "fatal error in internal/sync.(*Mutex).unlockSlow", ""},
		{"stack overflow", "fatal error in ", ".crashRecurse"},
		{"out of memory", "fatal error in ", ".crashAllocHuge"},
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
			if !strings.HasPrefix(prev.cause, tc.wantCause) || !strings.Contains(prev.cause, tc.wantIn) {
				t.Errorf("cause = %q, want it to start %q and name %q", prev.cause, tc.wantCause, tc.wantIn)
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
// frame it struck in: the dying goroutine's first outside the runtime, which
// for an error raised on the system stack is the goroutine after the
// "runtime stack:" block; failing that, the first frame below the runtime's
// throw and fatal.
//
// Mutants: take the first line whatever it is (the trace rows); stop skipping
// the throw/fatal frames (the cause names runtime.throw); search the
// "runtime stack:" block ahead of the goroutine (the stack-overflow row names
// runtime.newstack); stop passing over the runtime's frames (the
// out-of-memory row names runtime.systemstack_switch); count crashExcerpt's
// cut marker as a frame (the cut row).
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
		{"a stack overflow",
			"\nruntime stack:\nruntime.throw({0x14cd4cb?, 0x200000008?})\n\t/go/src/runtime/panic.go:1243 +0x48 fp=0x7ffd sp=0x7ffd pc=0x492c88\n" +
				"runtime.newstack()\n\t/go/src/runtime/stack.go:1207 +0x5dd fp=0x7ffd sp=0x7ffd pc=0x471cfd\n" +
				"runtime.morestack()\n\t/go/src/runtime/asm_amd64.s:650 +0x7b fp=0x7ffd sp=0x7ffd pc=0x49895b\n\n" +
				"goroutine 36 gp=0x3f53 m=0 mp=0x5ad9 [running]:\n" +
				"github.com/vampiricwulf/Moombox/internal/chat.(*Parser).walk(0xe9d?)\n\t/src/internal/chat/parse.go:11 +0x9b\n" +
				"github.com/vampiricwulf/Moombox/internal/chat.(*Parser).walk(0x0?)\n\t/src/internal/chat/parse.go:14 +0x75\n",
			"fatal error in github.com/vampiricwulf/Moombox/internal/chat.(*Parser).walk"},
		{"out of memory",
			"\nruntime stack:\nruntime.throw({0x14e2324?, 0x297a?})\n\t/go/src/runtime/panic.go:1243 +0x48\n" +
				"runtime.sysMapOS(0x25ea, 0x2000, {0x14b5, 0x4})\n\t/go/src/runtime/mem_linux.go:175 +0x13b\n" +
				"runtime.systemstack(0x47e7ff)\n\t/go/src/runtime/asm_amd64.s:531 +0x4a\n\n" +
				"goroutine 7 gp=0x23a0 m=0 mp=0x545e [running]:\n" +
				"runtime.systemstack_switch()\n\t/go/src/runtime/asm_amd64.s:481 +0x8\n" +
				"runtime.(*mheap).alloc(0x0?, 0x0?, 0x0?)\n\t/go/src/runtime/mheap.go:1002 +0x57\n" +
				"runtime.mallocgc(0x200000000000, 0x52f990, 0x1)\n\t/go/src/runtime/malloc.go:1137 +0x11a\n" +
				"runtime.makeslice(0x0?, 0x0?, 0x0?)\n\t/go/src/runtime/slice.go:117 +0x49\n" +
				"github.com/vampiricwulf/Moombox/internal/engine.(*Downloader).grow(...)\n\t/src/internal/engine/x.go:21 +0x7b\n" +
				"created by github.com/vampiricwulf/Moombox/internal/engine.(*Downloader).Run in goroutine 1\n\t/src/internal/engine/x.go:9 +0x1\n",
			"fatal error in github.com/vampiricwulf/Moombox/internal/engine.(*Downloader).grow"},
		{"the runtime's frames alone, cut",
			"\nruntime stack:\nruntime.throw({0x1?, 0x2?})\n\t/go/src/runtime/panic.go:1243 +0x48\n" +
				"runtime.newstack()\n\t/go/src/runtime/stack.go:1207 +0x5dd\n… 81234 more bytes",
			"fatal error in runtime.newstack"},
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

// TestCrashOutputThatCannotBeMovedAside pins what a start does when the crash
// output cannot be moved to its kept name — on Windows, a viewer, a scanner or
// the indexer holding either file without delete sharing (simulated); anywhere,
// a kept name rename cannot replace (a directory, here). It used to report the
// output kept in place, as the crash file itself, and then arm this start's
// capture on that same file, which truncated it: the log and the Recovered
// From Crash notice named an empty file as holding the whole output.
//
//   - The move fails and a copy can be made: the kept name holds the output,
//     cut to crashKeepBytes, and the crash file is armed empty for this start.
//   - The copy fails too: nothing is named as holding the whole output, the
//     notice says the rest could not be kept, and the log has the excerpt.
//
// Mutants: no copy fallback (the first row's kept file is missing); kept left
// as the crash file on failure (the second row's notice names an empty file);
// the copy unbounded (the first row's kept file is the whole output).
func TestCrashOutputThatCannotBeMovedAside(t *testing.T) {
	var big strings.Builder
	big.WriteString("panic: probe crash\n\ngoroutine 9 [running]:\n")
	for big.Len() < crashKeepBytes+crashKeepBytes/4 {
		big.WriteString("main.f()\n\t/src/main.go:1 +0x1\n")
	}
	body := big.String()

	for _, tc := range []struct {
		name       string
		renameFail bool // simulated: a sharing violation
		keptIsDir  bool // real: rename and copy both refuse a directory
		wantKept   bool
	}{
		{"the move is refused, the copy is not", true, false, true},
		{"the move and the copy are refused", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "moombox.log")
			crashPath, err := filepath.Abs(crashOutputPath(logPath))
			if err != nil {
				t.Fatal(err)
			}
			keptPath := keptCrashPath(crashPath)
			if err := os.WriteFile(crashPath, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.renameFail {
				prevRename := renameCrashOutput
				renameCrashOutput = func(string, string) error {
					return errors.New("the process cannot access the file because it is being used by another process")
				}
				t.Cleanup(func() { renameCrashOutput = prevRename })
			}
			if tc.keptIsDir {
				if err := os.MkdirAll(filepath.Join(keptPath, "x"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			log, err := logger.New(logPath, "INFO", 1<<20, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })

			s := &runState{log: log}
			prev := s.captureCrashOutput(logPath)
			log.Close()
			if !prev.captured() || prev.cause != "panic: probe crash" {
				t.Fatalf("captureCrashOutput = cause %q, captured %v; want the probe crash", prev.cause, prev.captured())
			}
			desc, _ := crashRecoveredNotice("2", prev, "moombox.log")
			if info, err := os.Stat(crashPath); err != nil || info.Size() != 0 {
				t.Errorf("the crash file is not armed empty for this start (%v)", err)
			}
			logged, _ := os.ReadFile(logPath)
			if !strings.Contains(string(logged), "goroutine 9") {
				t.Errorf("moombox.log has no excerpt of the crash:\n%s", logged)
			}

			if !tc.wantKept {
				if prev.kept != "" {
					t.Errorf("kept = %q, want none: nothing holds the whole output", prev.kept)
				}
				if strings.Contains(desc, "the whole of it") || !strings.Contains(desc, "could not be kept") {
					t.Errorf("notice = %q, want it to say the rest could not be kept", desc)
				}
				return
			}
			if prev.kept != keptPath {
				t.Fatalf("kept = %q, want %q", prev.kept, keptPath)
			}
			data, err := os.ReadFile(keptPath)
			if err != nil {
				t.Fatalf("the output was not kept: %v", err)
			}
			if len(data) != crashKeepBytes || !strings.HasPrefix(body, string(data)) {
				t.Errorf("the kept output is %d bytes, want the first %d of the crash's", len(data), crashKeepBytes)
			}
			if !strings.Contains(desc, "the whole of it in "+keptPath) {
				t.Errorf("notice = %q, want it to name %s", desc, keptPath)
			}
		})
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
