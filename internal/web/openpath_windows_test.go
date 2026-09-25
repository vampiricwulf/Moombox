package web

import (
	"syscall"
	"testing"
)

// TestWindowsOpenPathForceQuotesTheTarget is W R-3's pin.
//
// Go's os/exec quotes an argument only when it contains a space, a tab or a
// quote, so a directory path like C:\Moombox\output\Stream=Archive reaches
// explorer.exe bare — and explorer's legacy parser splits an unquoted '='
// into separate arguments and silently opens nothing. The TUI's browser
// launch (openBrowserCmd, internal/tui/openbrowser_windows.go) has carried
// the force-quoted CmdLine for exactly this reason since it was verified
// empirically on watch?v= URLs; the folder-open path never got it.
//
// Windows-only by necessity: syscall.SysProcAttr has a CmdLine field on no
// other platform. TestOpenPathWindowsArmForceQuotes in server_test.go is the
// half a Linux runner can see.
//
// MUTANTS, one per assertion:
//   - delete the forceQuoteCmdLine call in openPathCommandFor (back to a bare
//     exec.Command) -> SysProcAttr is nil and the first assertion fails;
//   - write the quotes into the argv instead (exec.Command(program, `"`+
//     target+`"`)) -> the CmdLine assertion fails AND the Args assertion
//     fails, because explorer would then be handed a literally-quoted path;
//   - drop the embedded-quote strip -> the quoted-path row fails;
//   - drop the `if goos == "windows"` guard so every platform is quoted ->
//     the last rows fail, because xdg-open and open take their argv.
func TestWindowsOpenPathForceQuotesTheTarget(t *testing.T) {
	t.Run("a path with = and no spaces", func(t *testing.T) {
		const target = `C:\Moombox\output\Stream=Archive`
		cmd := openPathCommandFor("windows", target)
		if cmd.SysProcAttr == nil {
			t.Fatal("the Windows command carries no SysProcAttr — Go leaves an unquoted '=' for " +
				"explorer's legacy parser to split, and the folder never opens")
		}
		if got, want := cmd.SysProcAttr.CmdLine, `explorer.exe "`+target+`"`; got != want {
			t.Errorf("CmdLine = %q, want %q", got, want)
		}
		if len(cmd.Args) != 2 || cmd.Args[0] != "explorer.exe" || cmd.Args[1] != target {
			t.Errorf("Args = %v, want [explorer.exe %s] — the argv must stay exactly what it was; "+
				"Windows ignores it once CmdLine is set, and TestOpenPathCommandIsTheSameOnBothHosts "+
				"asserts it is byte-identical on every platform", cmd.Args, target)
		}
	})

	t.Run("an embedded quote cannot escape the quoting", func(t *testing.T) {
		// A Windows path cannot legally contain '"', so this is defensive
		// only — the same defence openBrowserCmd applies to URLs.
		cmd := openPathCommandFor("windows", `C:\out\a"b`)
		if cmd.SysProcAttr == nil {
			t.Fatal("no SysProcAttr on the Windows arm")
		}
		if got, want := cmd.SysProcAttr.CmdLine, `explorer.exe "C:\out\ab"`; got != want {
			t.Errorf("CmdLine = %q, want %q", got, want)
		}
	})

	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		t.Run(goos+" is not quoted", func(t *testing.T) {
			cmd := openPathCommandFor(goos, "/home/u/Stream=Archive")
			if cmd.SysProcAttr != nil {
				t.Errorf("%s carries SysProcAttr %+v — only Windows needs the command line "+
					"rewritten; everywhere else exec passes argv straight through",
					goos, cmd.SysProcAttr)
			}
		})
	}
}

// parseWindowsCmdLine hands a composed command line back to CommandLineToArgvW
// — the Win32 parser — and returns the argv it recovers.
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

// TestWindowsOpenPathCmdLineRoundTripsThroughTheWin32Parser closes the hole the
// assertions above cannot see: they check the BYTES of the composed command
// line, and bytes that look right can still parse wrong.
//
// Inside a quoted argument a run of backslashes immediately before the closing
// quote escapes it. So a target that ends in a separator — `D:\`, the value
// filepath.Dir returns for a file sitting in the root of a dedicated
// recordings disk, and what BOTH callers hand this helper — used to compose to
//
//	explorer.exe "D:\"
//
// which CommandLineToArgvW reads as the single argument `D:"`. That input
// WORKED before the force-quoting landed (Go emits it bare, quoting only on a
// space, a tab or a quote), so the '=' fix had traded one broken path shape
// for another. The run is doubled now, exactly as syscall.EscapeArg does.
//
// MUTANTS:
//   - delete the strings.Repeat line in openpath_windows.go -> the three rows
//     that end in a separator fail, each with a trailing '"' glued to the path;
//   - double every backslash rather than only the trailing run -> the rows
//     with interior separators fail, because a run not followed by the closing
//     quote is literal and the doubling survives into the argv;
//   - append exactly one backslash unconditionally -> the rows that do NOT end
//     in a separator fail, because they gain one they never had;
//   - drop the quote strip -> ONLY the quote-then-space row fails; the plain
//     embedded-quote row reassembles by coincidence (see its note below), which
//     is why that row is here.
func TestWindowsOpenPathCmdLineRoundTripsThroughTheWin32Parser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		wantArg string
	}{
		{"a drive root — filepath.Dir of a file in D:\\", `D:\`, `D:\`},
		{"the = path W R-3 is about", `C:\Moombox\output\Stream=Archive`, `C:\Moombox\output\Stream=Archive`},
		{"a drive root's = child", `D:\x=y`, `D:\x=y`},
		{"a space, an = and a trailing separator", `E:\x y=z\`, `E:\x y=z\`},
		{"a UNC share root", `\\server\share\`, `\\server\share\`},
		{"a plain path with no = at all", `C:\Users\Wulf\Videos`, `C:\Users\Wulf\Videos`},
		{"an embedded quote is stripped, never escaped", `C:\out\a"b`, `C:\out\ab`},
		// The row above cannot see a MISSING strip: `"C:\out\a"b"` happens to
		// reassemble as C:\out\ab, because the quote merely closes the quoted
		// run and `b` continues the same token. Put a space after the quote
		// and the token splits in two, which is what an un-stripped quote
		// really costs.
		{"an embedded quote followed by a space would split the argv", `C:\out\a"b c`, `C:\out\ab c`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := openPathCommandFor("windows", tc.target)
			if cmd.SysProcAttr == nil {
				t.Fatal("no SysProcAttr on the Windows arm")
			}
			line := cmd.SysProcAttr.CmdLine
			got := parseWindowsCmdLine(t, line)
			want := []string{"explorer.exe", tc.wantArg}
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Errorf("CmdLine %q parses to %q, want %q — explorer would be handed that argv, "+
					"not the directory the user asked for", line, got, want)
			}
		})
	}
}
