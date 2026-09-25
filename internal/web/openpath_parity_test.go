package web

import (
	"os"
	"strings"
	"testing"
)

// composerSignature is the first line of both copies of forceQuoteCmdLine.
// The extraction starts here and stops at the first line that is exactly a
// closing brace, which is the end of a top-level function body.
const composerSignature = "func forceQuoteCmdLine(cmd *exec.Cmd, program, target string) {\n"

// TestWindowsCmdLineComposersAreByteIdentical is the mechanical tie between
// the two Windows command-line composers: forceQuoteCmdLine in
// internal/web/openpath_windows.go and its copy in
// internal/tui/openbrowser_windows.go.
//
// The copy is forced. internal/tui may not import internal/web (the import
// fence) and SysProcAttr.CmdLine exists only in the Windows syscall package,
// so the shared body cannot be hoisted into an untagged file — the two must
// simply be kept the same. Before this test nothing but a comment said so,
// and a reader who had checked one composer had checked only one.
//
// It reads SOURCE rather than calling either function on purpose:
//
//   - the package under test cannot call the TUI's copy at all (the fence
//     runs the other way too: internal/web must not reach into internal/tui's
//     API, and the copy is unexported besides);
//   - this file is UNTAGGED, so it runs on ubuntu, where BOTH
//     _windows_test.go round-trip tables are excluded from the build and CI
//     would otherwise see no composer coverage whatsoever.
//
// The two round-trip tables stay the behavioural pins (they run the real
// CommandLineToArgvW on Windows); this one pins that there is nothing to
// check twice. The os.ReadFile technique is TestOpenPathWindowsArmForceQuotes'.
//
// MUTANTS:
//   - change TrimRight to TrimSuffix in either copy -> "drifted apart";
//   - delete the strings.Repeat line from either copy -> same, and on Windows
//     that copy's round-trip table fails too;
//   - rename a parameter in either copy -> same (the signature line is inside
//     the compared region, which is the point: the bodies must be usable as
//     one another's documentation).
func TestWindowsCmdLineComposersAreByteIdentical(t *testing.T) {
	webBody := composerBody(t, "openpath_windows.go")
	tuiBody := composerBody(t, "../tui/openbrowser_windows.go")

	if webBody != tuiBody {
		t.Errorf("the two Windows CmdLine composers have drifted apart — the import fence makes them "+
			"two copies of one function, so a reader who checks one is relying on the other being "+
			"identical\n\ninternal/web/openpath_windows.go:\n%s\ninternal/tui/openbrowser_windows.go:\n%s",
			webBody, tuiBody)
	}
}

// composerBody returns forceQuoteCmdLine's declaration and body from the named
// source file, newlines normalised, ending at the closing brace.
func composerBody(t *testing.T, path string) string {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := strings.ReplaceAll(string(src), "\r\n", "\n")

	start := strings.Index(text, composerSignature)
	if start < 0 {
		t.Fatalf("%s no longer declares %q — the two composers can only be pinned to each other "+
			"while both spell the helper the same way", path, strings.TrimSuffix(composerSignature, "\n"))
	}
	rest := text[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("%s: forceQuoteCmdLine has no closing brace at column 0", path)
	}
	return rest[:end+len("\n}\n")]
}
