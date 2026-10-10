package main

import (
	"context"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCLIAddHonoursConfigOnEitherSide runs main() in a child process (the
// test binary re-executed). main used to recognise `add` only as argv[1], so
// `moombox -config X add URL` fell through to the daemon with its arguments
// silently dropped, and `add` loaded the config from the cwd search whatever
// -config said. Both orders now reach addVideo with the named config, which
// here does not exist, so it must exit 1 naming that path.
//
// Mutant: check os.Args[1] == "add" before parsing flags again — the first
// row boots the daemon instead (and fails on the timeout).
func TestCLIAddHonoursConfigOnEitherSide(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Fields(os.Getenv("MOOMBOX_CLI_ARGS"))...)
		main()
		os.Exit(0)
	}
	for _, args := range []string{
		"-config /nonexistent/moombox-cli-test.toml add dQw4w9WgXcQ",
		"add -config /nonexistent/moombox-cli-test.toml dQw4w9WgXcQ",
	} {
		t.Run(args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIAddHonoursConfigOnEitherSide$")
			cmd.Dir = t.TempDir() // a wrong dispatch writes a daemon's files here, not into the package
			cmd.Env = append(os.Environ(),
				"MOOMBOX_CLI_HELPER=1",
				"MOOMBOX_CLI_ARGS="+args,
				"_MOOMBOX_CHILD=1", // a wrong dispatch boots run() here, not a launcher
				"MOOMBOX_NO_TUI=1",
			)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("the command did not exit (it booted the daemon?):\n%s", out)
			}
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("exit = %v, want status 1:\n%s", err, out)
			}
			if !strings.Contains(string(out), "No config file at /nonexistent/moombox-cli-test.toml") {
				t.Errorf("add did not use the named config:\n%s", out)
			}
		})
	}
}

// TestCLIVersionEqualsTrueSkipsTheLauncher: only the literal -version and
// --version were answered before the launcher gate, so `-version=true` went
// through the launcher — taking the single-instance lock (failing while the
// daemon runs) and spawning a child just to print a line. Run WITHOUT
// _MOOMBOX_CHILD, the way a user would: a launcher dispatch here would spawn
// this test binary again rather than print the version.
//
// Mutant: move the *showVersion check back below the launcher gate.
func TestCLIVersionEqualsTrueSkipsTheLauncher(t *testing.T) {
	if os.Getenv("MOOMBOX_CLI_HELPER") == "1" {
		os.Args = append([]string{"moombox"}, strings.Fields(os.Getenv("MOOMBOX_CLI_ARGS"))...)
		main()
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCLIVersionEqualsTrueSkipsTheLauncher$")
	cmd.Dir = t.TempDir()
	env := []string{"MOOMBOX_CLI_HELPER=1", "MOOMBOX_CLI_ARGS=-version=true", "MOOMBOX_NO_TUI=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "_MOOMBOX_CHILD=") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the command did not exit:\n%s", out)
	}
	if err != nil || !strings.HasPrefix(string(out), "moombox "+version) {
		t.Fatalf("exit = %v, output = %q; want the version line and status 0", err, out)
	}
}

// TestUsageNamesTheAddSubcommand: -h printed the daemon's flags only, and
// `add` appeared nowhere until it was run without an argument.
func TestUsageNamesTheAddSubcommand(t *testing.T) {
	var b strings.Builder
	prev := flagOutput(&b)
	defer flagOutput(prev)
	printUsage()
	if !strings.Contains(b.String(), "add <video_id_or_url>") {
		t.Errorf("usage does not mention add:\n%s", b.String())
	}
}

// flagOutput swaps flag.CommandLine's output and returns the previous one.
func flagOutput(w io.Writer) io.Writer {
	prev := flag.CommandLine.Output()
	flag.CommandLine.SetOutput(w)
	return prev
}
