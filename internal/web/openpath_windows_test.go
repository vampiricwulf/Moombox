package web

import "testing"

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
