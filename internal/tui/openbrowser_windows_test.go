package tui

import (
	"syscall"
	"testing"
)

// parseWindowsCmdLine hands a composed command line back to CommandLineToArgvW
// — the Win32 parser — and returns the argv it recovers.
//
// A near-copy of the helper beside the web twin
// (internal/web/openpath_windows_test.go). internal/tui may not import
// internal/web, which is the same fence that forces the two composers apart;
// a test helper cannot cross it either.
//
// The block CommandLineToArgvW LocalAllocs is deliberately not freed:
// syscall.LocalFree wants a Handle, and the only way to make one from a
// *[8192]*[8192]uint16 is an unsafe.Pointer conversion. A handful of small
// blocks that live for the test binary is the cheaper trade than an unsafe
// import in a test whose entire job is to be obviously correct.
func parseWindowsCmdLine(t *testing.T, line string) []string {
	t.Helper()
	utf16Line, err := syscall.UTF16PtrFromString(line)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", line, err)
	}
	var argc int32
	argv, err := syscall.CommandLineToArgv(utf16Line, &argc)
	if err != nil {
		t.Fatalf("CommandLineToArgv(%q): %v", line, err)
	}
	out := make([]string, 0, argc)
	for _, w := range (*argv)[:argc] {
		out = append(out, syscall.UTF16ToString((*w)[:]))
	}
	return out
}

// TestOpenBrowserCmdRoundTripsThroughTheWin32Parser is the twin of
// TestWindowsOpenPathCmdLineRoundTripsThroughTheWin32Parser in internal/web.
//
// openBrowserCmd and forceQuoteCmdLine (internal/web/openpath_windows.go)
// compose the same string by hand, because internal/tui may not import
// internal/web and SysProcAttr.CmdLine exists only in the Windows syscall
// package. Nothing mechanically ties the two together, so each carries its own
// round-trip pin and the two tables share their rows: a divergence shows up as
// one file's table going red.
//
// The trailing-backslash rows are UNREACHABLE here — a URL cannot end in a
// '\', and this function is only ever handed one. They are pinned anyway,
// because the whole point of keeping the two composers byte-identical is that
// a reader can check one and trust the other; a row that exists on only one
// side breaks that.
//
// MUTANTS:
//   - delete the strings.Repeat line in openbrowser_windows.go -> the three
//     rows ending in a separator fail with a trailing '"' glued on;
//   - drop the quote strip -> ONLY the quote-then-space row fails; the plain
//     embedded-quote row reassembles by coincidence (see its note below), which
//     is why that row is here;
//   - drop the SysProcAttr assignment -> every row fails at the nil check.
//
// Note what this test canNOT see: CommandLineToArgvW is not what splits a bare
// '=' — explorer's own legacy parser is, and that is unassertable from here.
// So the watch?v= row is a shape check, and explorer's behaviour on it stays
// the field gate the original empirical verification established.
func TestOpenBrowserCmdRoundTripsThroughTheWin32Parser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		url     string
		wantArg string
	}{
		{"the watch?v= URL the force-quoting exists for", "https://www.youtube.com/watch?v=abc123", "https://www.youtube.com/watch?v=abc123"},
		{"a drive root — unreachable for a URL, pinned for parity", `D:\`, `D:\`},
		{"a space, an = and a trailing separator", `E:\x y=z\`, `E:\x y=z\`},
		{"a UNC share root", `\server\share\`, `\server\share\`},
		{"a plain URL with no = at all", "https://github.com/vampiricwulf/Moombox", "https://github.com/vampiricwulf/Moombox"},
		{"an embedded quote is stripped, never escaped", `https://x/a"b`, "https://x/ab"},
		// The row above cannot see a MISSING strip: `"https://x/a"b"` happens
		// to reassemble as https://x/ab, because the quote merely closes the
		// quoted run and `b` continues the same token. Put a space after the
		// quote and the token splits in two, which is what an un-stripped
		// quote really costs.
		{"an embedded quote followed by a space would split the argv", `https://x/a"b c`, "https://x/ab c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := openBrowserCmd(tc.url)
			if cmd.SysProcAttr == nil {
				t.Fatal("openBrowserCmd built no SysProcAttr — without a forced CmdLine Go leaves an " +
					"unquoted '=' for explorer's legacy parser to split, and nothing opens")
			}
			line := cmd.SysProcAttr.CmdLine
			got := parseWindowsCmdLine(t, line)
			want := []string{"explorer.exe", tc.wantArg}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("CmdLine %q parses to %q, want %q", line, got, want)
			}
		})
	}
}
